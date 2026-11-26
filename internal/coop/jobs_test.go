// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/tacplus-dev/coop/tacplus/chassis/token"
	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
	kernel_pb "github.com/tacplus-dev/coop/tacplus/types/services/kernel"
	token_pb "github.com/tacplus-dev/coop/tacplus/types/sys/token"
	tokenrequest_pb "github.com/tacplus-dev/coop/tacplus/types/sys/tokenrequest"
)

// fakeMint mints a workload token under the coop and remembers what it did.
type fakeMint struct {
	minted  []*tokenrequest_pb.WorkloadMintRequest
	revoked []string
	// revokeTraces is the trace_id each revoke carried.
	revokeTraces []string
}

func (m *fakeMint) MintWorkload(_ context.Context, req *tokenrequest_pb.WorkloadMintRequest, _ ...grpc.CallOption) (*tokenrequest_pb.WorkloadMintResponse, error) {
	m.minted = append(m.minted, req)
	parent := req.GetParentSpiffeId()
	if parent == "" {
		parent = coopID
	}
	tok := token.NewImmutableToken(&token_pb.Token{Payload: map[string]string{}}).SetSubject(parent + "/workloads/" + req.GetWorkloadName())
	tok.Proto().Base64 = "hdr.payload.sig" // kernel returns the signed, encoded token
	return &tokenrequest_pb.WorkloadMintResponse{BootstrapToken: tok.Proto()}, nil
}
func (m *fakeMint) RevokeWorkload(_ context.Context, req *kernel_pb.WorkloadRevokeRequest, _ ...grpc.CallOption) error {
	m.revoked = append(m.revoked, req.GetSpiffeId())
	m.revokeTraces = append(m.revokeTraces, req.GetTraceId())
	return nil
}

func jobsCfg() *pb.Config {
	return &pb.Config{Mode: pb.Mode_MODE_JOBS, PigeonsNamespace: "schematic-pigeons", Kinds: []*pb.Kind{{
		Name: "echo.ping", Image: "example.test/pigeon:1", Cpu: "250m", Memory: "256Mi",
	}}}
}

func liveRun(id string, state pb.RunState) *pb.LiveRun {
	return &pb.LiveRun{RunId: id, ThreadId: id, TenantNamespace: acme, CallerSpiffeId: dogAcme, Kind: "echo.ping",
		State: state, DeadlineMs: time.Unix(1_700_000_000, 0).Add(10 * time.Minute).UnixMilli()}
}

func TestJobRunnerStartCreatesSecretAndJob(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	mint := &fakeMint{}
	r := &JobRunner{Client: client, Mint: mint, Bootstrap: []byte("endpoints: []\n"), FleetNamespace: "schematic", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	cfg := jobsCfg()
	run := liveRun("01abc", pb.RunState_RUN_STATE_REQUESTED)

	id, err := r.Start(ctx, cfg, run, cfg.Kinds[0], []byte("task"))
	if err != nil {
		t.Fatal(err)
	}
	// The pigeon is the dog's: minted under the caller, in the caller's tenant.
	if id != dogAcme+"/workloads/pigeon-01abc" || run.GetJobName() != "pigeon-01abc" {
		t.Fatalf("pigeon id %q job %q", id, run.GetJobName())
	}
	// The identity is terminal and lives no longer than the bootstrap cap.
	if m := mint.minted[0]; !m.GetTerminal() || m.GetWorkloadName() != "pigeon-01abc" || m.GetExp() != 1_700_000_000+300 || m.GetParentSpiffeId() != dogAcme {
		t.Fatalf("mint request %v", m)
	}
	secret, err := client.CoreV1().Secrets("schematic-pigeons").Get(ctx, "pigeon-01abc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["bootstrap.yaml"]) != "endpoints: []\n" || string(secret.Data["bootstrap.token"]) != "hdr.payload.sig" {
		t.Fatalf("secret data %v", secret.Data)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Job" || secret.OwnerReferences[0].Name != "pigeon-01abc" {
		t.Fatalf("secret is not owned by its job: %v", secret.OwnerReferences)
	}
	job, err := client.BatchV1().Jobs("schematic-pigeons").Get(ctx, "pigeon-01abc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Fatalf("a pigeon must pull its image every time: %v", c.ImagePullPolicy)
	}
	if c.Image != "example.test/pigeon:1" || *job.Spec.BackoffLimit != 0 || pod.RestartPolicy != corev1.RestartPolicyNever ||
		*job.Spec.ActiveDeadlineSeconds != 600+30 || *job.Spec.TTLSecondsAfterFinished != 60 {
		t.Fatalf("job spec %+v", job.Spec)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("a pigeon must hold no Kubernetes identity")
	}
	advertise := false
	for _, e := range c.Env {
		if e.Name == "SERVICE_ADVERTISE_HOST" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "status.podIP" {
			advertise = true
		}
	}
	if !advertise {
		t.Fatal("the pigeon must advertise its pod IP as its health endpoint host")
	}
	if pod.DNSConfig == nil || len(pod.DNSConfig.Searches) != 1 || pod.DNSConfig.Searches[0] != "schematic.svc.cluster.local" {
		t.Fatalf("the fleet's bare service names must resolve from the pigeons namespace: %v", pod.DNSConfig)
	}
	if *pod.SecurityContext.FSGroup != 65532 || *pod.Volumes[0].Secret.DefaultMode != 0o440 {
		t.Fatalf("the pigeon (uid 65532) must be able to read its Secret: fsGroup %v mode %o", pod.SecurityContext.FSGroup, *pod.Volumes[0].Secret.DefaultMode)
	}
	if !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatalf("pigeon container is not locked down: %+v", c.SecurityContext)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"KERNEL_BOOTSTRAP_TOKEN_FILE": "/run/pigeon/bootstrap.token", "BOOTSTRAP_CONFIG": "/run/pigeon/bootstrap.yaml",
		"PIGEON_RUN_ID": "01abc", "PIGEON_THREAD_ID": "01abc", "PIGEON_TENANT": acme, "PIGEON_KIND": "echo.ping", "PIGEON_CALLER": dogAcme,
	} {
		if env[k] != want {
			t.Errorf("env %s = %q, want %q", k, env[k], want)
		}
	}
	if c.Resources.Limits.Cpu().String() != "250m" || c.Resources.Limits.Memory().String() != "256Mi" {
		t.Errorf("limits %v", c.Resources.Limits)
	}
	if job.Labels[labelRun] != "01abc" || job.Annotations[annotationTenant] != acme {
		t.Errorf("labels %v annotations %v", job.Labels, job.Annotations)
	}

	// A kind with a bad quantity or no image never reaches the cluster.
	bad := &pb.Kind{Name: "bad", Image: "x", Cpu: "lots"}
	if _, err := r.Start(ctx, cfg, liveRun("01bad", pb.RunState_RUN_STATE_REQUESTED), bad, nil); err == nil || !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("bad cpu: %v", err)
	}
	if _, err := r.Start(ctx, cfg, liveRun("01img", pb.RunState_RUN_STATE_REQUESTED), &pb.Kind{Name: "noimg"}, nil); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("no image: %v", err)
	}
	if len(mint.minted) != 1 {
		t.Fatalf("validation must come before minting; minted %d", len(mint.minted))
	}
}

func TestJobRunnerStopCheckReconcile(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	mint := &fakeMint{}
	r := &JobRunner{Client: client, Mint: mint, FleetNamespace: "schematic", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	cfg := jobsCfg()
	run := liveRun("01abc", pb.RunState_RUN_STATE_REQUESTED)
	id, err := r.Start(ctx, cfg, run, cfg.Kinds[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	run.PigeonSpiffeId = id
	run.State = pb.RunState_RUN_STATE_RUNNING

	// Running: not ended.
	if ended, _, _, err := r.Check(ctx, cfg, run); err != nil || ended {
		t.Fatalf("running job: ended=%v err=%v", ended, err)
	}
	// Failed condition ends the run FAILED.
	job, _ := client.BatchV1().Jobs(cfg.PigeonsNamespace).Get(ctx, "pigeon-01abc", metav1.GetOptions{})
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	if _, err := client.BatchV1().Jobs(cfg.PigeonsNamespace).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ended, reason, detail, err := r.Check(ctx, cfg, run)
	if err != nil || !ended || reason != pb.EndReason_END_REASON_FAILED || !strings.Contains(detail, "BackoffLimitExceeded") {
		t.Fatalf("failed job: %v %v %q %v", ended, reason, detail, err)
	}

	// Stop deletes the Job and Secret and revokes the identity, under the
	// trace of what stopped it.
	if err := r.Stop(ctx, cfg, run, "trace-stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BatchV1().Jobs(cfg.PigeonsNamespace).Get(ctx, "pigeon-01abc", metav1.GetOptions{}); err == nil {
		t.Fatal("job survived Stop")
	}
	if _, err := client.CoreV1().Secrets(cfg.PigeonsNamespace).Get(ctx, "pigeon-01abc", metav1.GetOptions{}); err == nil {
		t.Fatal("secret survived Stop")
	}
	if len(mint.revoked) != 1 || mint.revoked[0] != id || mint.revokeTraces[0] != "trace-stop" {
		t.Fatalf("revoked %v, trace_ids %q", mint.revoked, mint.revokeTraces)
	}
	// Stopping again is a no-op, and a gone Job reads as ended.
	if err := r.Stop(ctx, cfg, run, ""); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if ended, reason, _, _ := r.Check(ctx, cfg, run); !ended || reason != pb.EndReason_END_REASON_FAILED {
		t.Fatalf("gone job: %v %v", ended, reason)
	}

	// Reconcile: an orphan Job is deleted; a live run without a Job is returned.
	orphan := liveRun("01orphan", pb.RunState_RUN_STATE_REQUESTED)
	if _, err := r.Start(ctx, cfg, orphan, cfg.Kinds[0], nil); err != nil {
		t.Fatal(err)
	}
	kept := liveRun("01kept", pb.RunState_RUN_STATE_REQUESTED)
	if _, err := r.Start(ctx, cfg, kept, cfg.Kinds[0], nil); err != nil {
		t.Fatal(err)
	}
	kept.State = pb.RunState_RUN_STATE_RUNNING
	lost := liveRun("01lost", pb.RunState_RUN_STATE_STARTING)
	missing, err := r.Reconcile(ctx, cfg, map[string]*pb.LiveRun{"01kept": kept, "01lost": lost, "01ended": liveRun("01ended", pb.RunState_RUN_STATE_ENDED)})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0].GetRunId() != "01lost" {
		t.Fatalf("missing %v", missing)
	}
	if _, err := client.BatchV1().Jobs(cfg.PigeonsNamespace).Get(ctx, "pigeon-01orphan", metav1.GetOptions{}); err == nil {
		t.Fatal("orphan job survived reconcile")
	}
	if _, err := client.BatchV1().Jobs(cfg.PigeonsNamespace).Get(ctx, "pigeon-01kept", metav1.GetOptions{}); err != nil {
		t.Fatalf("live run's job was deleted: %v", err)
	}
}

// A shared coop serves two tenants, each with its own pigeons namespace.
const (
	globex    = "/tenants/globex"
	dogGlobex = "spiffe://local.dev/tenants/globex01/services/tenantecho"
)

func twoTenantCfg() *pb.Config {
	cfg := jobsCfg()
	cfg.PigeonsNamespace = "schematic-pigeons"
	cfg.PigeonsNamespaces = map[string]string{acme: "tac-seatrials-acme0001", globex: "tac-seatrials-globex01"}
	return cfg
}

func globexRun(id string, state pb.RunState) *pb.LiveRun {
	r := liveRun(id, state)
	r.TenantNamespace, r.CallerSpiffeId = globex, dogGlobex
	return r
}

// jobsIn lists the pigeon Jobs and Secrets in ns by name.
func jobsIn(t *testing.T, client *fake.Clientset, ns string) (jobs, secrets []string) {
	t.Helper()
	ctx := context.Background()
	jl, err := client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jl.Items {
		jobs = append(jobs, j.Name)
	}
	sl, err := client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sl.Items {
		secrets = append(secrets, s.Name)
	}
	return jobs, secrets
}

func TestJobRunnerRunsEachTenantInItsOwnNamespace(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	mint := &fakeMint{}
	r := &JobRunner{Client: client, Mint: mint, FleetNamespace: "tac-seatrials", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	cfg := twoTenantCfg()

	a := liveRun("01aaa", pb.RunState_RUN_STATE_REQUESTED)
	idA, err := r.Start(ctx, cfg, a, cfg.Kinds[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	b := globexRun("01bbb", pb.RunState_RUN_STATE_REQUESTED)
	idB, err := r.Start(ctx, cfg, b, cfg.Kinds[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	// Each run's Job and Secret are in its own tenant's namespace, and only
	// there; its identity is its own dog's.
	for _, c := range []struct {
		run        *pb.LiveRun
		id, dog    string
		own, other string
	}{
		{a, idA, dogAcme, "tac-seatrials-acme0001", "tac-seatrials-globex01"},
		{b, idB, dogGlobex, "tac-seatrials-globex01", "tac-seatrials-acme0001"},
	} {
		name := "pigeon-" + c.run.GetRunId()
		if c.run.GetJobNamespace() != c.own || c.id != c.dog+"/workloads/"+name {
			t.Fatalf("run %s: job namespace %q, pigeon %q", c.run.GetRunId(), c.run.GetJobNamespace(), c.id)
		}
		if _, err := client.BatchV1().Jobs(c.own).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Fatalf("run %s's job is not in %s: %v", c.run.GetRunId(), c.own, err)
		}
		if _, err := client.CoreV1().Secrets(c.own).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Fatalf("run %s's secret is not in %s: %v", c.run.GetRunId(), c.own, err)
		}
		if _, err := client.BatchV1().Jobs(c.other).Get(ctx, name, metav1.GetOptions{}); err == nil {
			t.Fatalf("run %s's job is in the other tenant's %s", c.run.GetRunId(), c.other)
		}
		if _, err := client.CoreV1().Secrets(c.other).Get(ctx, name, metav1.GetOptions{}); err == nil {
			t.Fatalf("run %s's secret is in the other tenant's %s", c.run.GetRunId(), c.other)
		}
	}
	if jobs, secrets := jobsIn(t, client, "schematic-pigeons"); len(jobs)+len(secrets) != 0 {
		t.Fatalf("the one-namespace layout's namespace is used beside the map: %v %v", jobs, secrets)
	}
	// A tenant with no entry is refused before its identity is minted.
	other := liveRun("01ccc", pb.RunState_RUN_STATE_REQUESTED)
	other.TenantNamespace = "/tenants/initech"
	if _, err := r.Start(ctx, cfg, other, cfg.Kinds[0], nil); err == nil || !strings.Contains(err.Error(), "/tenants/initech") {
		t.Fatalf("unmapped tenant: %v", err)
	}
	if len(mint.minted) != 2 {
		t.Fatalf("an unmapped tenant's pigeon was minted: %d mints", len(mint.minted))
	}

	// A config change after start moves no live run: Check and Stop go to the
	// namespace the run started in.
	moved := twoTenantCfg()
	moved.PigeonsNamespaces = map[string]string{acme: "tac-seatrials-elsewhere", globex: "tac-seatrials-acme0001"}
	a.State, b.State = pb.RunState_RUN_STATE_RUNNING, pb.RunState_RUN_STATE_RUNNING
	if ended, _, detail, err := r.Check(ctx, moved, a); err != nil || ended {
		t.Fatalf("a's job is not found where it started: ended=%v %q %v", ended, detail, err)
	}
	if err := r.Stop(ctx, moved, b, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BatchV1().Jobs("tac-seatrials-globex01").Get(ctx, "pigeon-01bbb", metav1.GetOptions{}); err == nil {
		t.Fatal("stop did not delete b's job in its own namespace")
	}
	if _, err := client.BatchV1().Jobs("tac-seatrials-acme0001").Get(ctx, "pigeon-01aaa", metav1.GetOptions{}); err != nil {
		t.Fatalf("stopping b touched a's namespace: %v", err)
	}
}

func TestJobRunnerReconcilesEveryTenantNamespace(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	r := &JobRunner{Client: client, Mint: &fakeMint{}, FleetNamespace: "tac-seatrials", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	cfg := twoTenantCfg()

	kept := liveRun("01kept", pb.RunState_RUN_STATE_REQUESTED)
	if _, err := r.Start(ctx, cfg, kept, cfg.Kinds[0], nil); err != nil {
		t.Fatal(err)
	}
	kept.State = pb.RunState_RUN_STATE_RUNNING
	orphan := globexRun("01orphan", pb.RunState_RUN_STATE_REQUESTED)
	if _, err := r.Start(ctx, cfg, orphan, cfg.Kinds[0], nil); err != nil {
		t.Fatal(err)
	}
	// A Job with a live run's label in a namespace that is not the run's is
	// not that run's pigeon: it is deleted, and the run is missing.
	stray := globexRun("01stray", pb.RunState_RUN_STATE_REQUESTED)
	if _, err := r.Start(ctx, cfg, stray, cfg.Kinds[0], nil); err != nil {
		t.Fatal(err)
	}
	stray.State, stray.JobNamespace = pb.RunState_RUN_STATE_RUNNING, "tac-seatrials-acme0001"
	lost := globexRun("01lost", pb.RunState_RUN_STATE_STARTING)
	lost.JobNamespace = "tac-seatrials-globex01"

	missing, err := r.Reconcile(ctx, cfg, map[string]*pb.LiveRun{"01kept": kept, "01lost": lost, "01stray": stray})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range missing {
		ids = append(ids, m.GetRunId())
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "01lost,01stray" {
		t.Fatalf("missing %v", ids)
	}
	if jobs, _ := jobsIn(t, client, "tac-seatrials-globex01"); len(jobs) != 0 {
		t.Fatalf("orphans survived in globex's namespace: %v", jobs)
	}
	if jobs, _ := jobsIn(t, client, "tac-seatrials-acme0001"); len(jobs) != 1 || jobs[0] != "pigeon-01kept" {
		t.Fatalf("acme's namespace after reconcile: %v", jobs)
	}

	// A namespace the coop may not list (its tenant's RBAC is gone) is
	// skipped, and its runs are left alone rather than ended.
	client.PrependReactor("list", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "tac-seatrials-globex01" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "", errors.New("no role"))
		}
		return false, nil, nil
	})
	missing, err = r.Reconcile(ctx, cfg, map[string]*pb.LiveRun{"01kept": kept, "01lost": lost})
	if err != nil {
		t.Fatalf("a forbidden namespace failed the coop's start: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("runs in an unlistable namespace were ended: %v", missing)
	}
}

func TestJobRunnerOneNamespaceLayout(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	r := &JobRunner{Client: client, Mint: &fakeMint{}, FleetNamespace: "schematic", Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
	cfg := jobsCfg()
	run := globexRun("01one", pb.RunState_RUN_STATE_REQUESTED)
	if _, err := r.Start(ctx, cfg, run, cfg.Kinds[0], nil); err != nil {
		t.Fatal(err)
	}
	if run.GetJobNamespace() != "schematic-pigeons" {
		t.Fatalf("job namespace %q", run.GetJobNamespace())
	}
	// A run started before job_namespace existed is in pigeons_namespace.
	run.JobNamespace, run.State = "", pb.RunState_RUN_STATE_RUNNING
	if ended, _, _, err := r.Check(ctx, cfg, run); err != nil || ended {
		t.Fatalf("pre-#17 run: ended=%v %v", ended, err)
	}
	if missing, err := r.Reconcile(ctx, cfg, map[string]*pb.LiveRun{"01one": run}); err != nil || len(missing) != 0 {
		t.Fatalf("pre-#17 run reconciled: %v %v", missing, err)
	}
	cfg.PigeonsNamespace = ""
	if _, err := r.Start(ctx, cfg, globexRun("01none", pb.RunState_RUN_STATE_REQUESTED), cfg.Kinds[0], nil); err == nil || !strings.Contains(err.Error(), "no pigeons namespace") {
		t.Fatalf("no namespace at all: %v", err)
	}
}

// A shared coop, end to end: each tenant's CreatePigeon puts its Job, its
// Secret and its stored run in its own namespace, never the other's; a tenant
// with no namespace is refused before anything is recorded or minted.
func TestSharedCoopCreatesEachRunInItsTenantNamespace(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	mint := &fakeMint{}
	h := newBareHarness()
	h.s.Jobs = &JobRunner{Client: client, Mint: mint, FleetNamespace: "tac-seatrials", Now: func() time.Time { return h.now }}
	cfg := twoTenantCfg()
	cfg.Kinds[0].MaxDeadlineMs = 600_000
	if err := h.s.CoopConfig(cfg); err != nil {
		t.Fatal(err)
	}

	ta, err := h.s.CreatePigeon(ctx, as(dogAcme, acme), h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	tb, err := h.s.CreatePigeon(ctx, as(dogGlobex, globex), h.request("echo.ping"))
	if err != nil {
		t.Fatal(err)
	}
	if ta.GetPigeonSpiffeId() != dogAcme+"/workloads/pigeon-"+ta.GetRunId() || tb.GetPigeonSpiffeId() != dogGlobex+"/workloads/pigeon-"+tb.GetRunId() {
		t.Fatalf("identities %q %q", ta.GetPigeonSpiffeId(), tb.GetPigeonSpiffeId())
	}
	for ns, want := range map[string]string{"tac-seatrials-acme0001": "pigeon-" + ta.GetRunId(), "tac-seatrials-globex01": "pigeon-" + tb.GetRunId()} {
		jobs, secrets := jobsIn(t, client, ns)
		if len(jobs) != 1 || jobs[0] != want || len(secrets) != 1 || secrets[0] != want {
			t.Fatalf("%s holds jobs %v secrets %v, want only %s", ns, jobs, secrets, want)
		}
	}
	if h.runs.runs[ta.GetRunId()].GetJobNamespace() != "tac-seatrials-acme0001" || h.runs.runs[tb.GetRunId()].GetJobNamespace() != "tac-seatrials-globex01" {
		t.Fatalf("stored runs do not pin their namespaces: %v %v", h.runs.runs[ta.GetRunId()], h.runs.runs[tb.GetRunId()])
	}

	before := len(h.runs.runs)
	_, err = h.s.CreatePigeon(ctx, as("spiffe://local.dev/tenants/initech1/services/tenantecho", "/tenants/initech"), h.request("echo.ping"))
	wantCode(t, err, codes.FailedPrecondition, "no pigeons namespace for /tenants/initech")
	if len(h.runs.runs) != before || len(mint.minted) != 2 {
		t.Fatalf("a refused tenant left a run (%d, was %d) or a mint (%d)", len(h.runs.runs), before, len(mint.minted))
	}
}
