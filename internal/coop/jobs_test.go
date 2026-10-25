// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

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
}

func (m *fakeMint) MintWorkload(_ context.Context, req *tokenrequest_pb.WorkloadMintRequest, _ ...grpc.CallOption) (*tokenrequest_pb.WorkloadMintResponse, error) {
	m.minted = append(m.minted, req)
	tok := token.NewImmutableToken(&token_pb.Token{Payload: map[string]string{}}).SetSubject(coopID + "/workloads/" + req.GetWorkloadName())
	tok.Proto().Base64 = "hdr.payload.sig" // kernel returns the signed, encoded token
	return &tokenrequest_pb.WorkloadMintResponse{BootstrapToken: tok.Proto()}, nil
}
func (m *fakeMint) RevokeWorkload(_ context.Context, req *kernel_pb.WorkloadRevokeRequest, _ ...grpc.CallOption) error {
	m.revoked = append(m.revoked, req.GetSpiffeId())
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
	if id != coopID+"/workloads/pigeon-01abc" || run.GetJobName() != "pigeon-01abc" {
		t.Fatalf("pigeon id %q job %q", id, run.GetJobName())
	}
	// The identity is terminal and lives no longer than the bootstrap cap.
	if m := mint.minted[0]; !m.GetTerminal() || m.GetWorkloadName() != "pigeon-01abc" || m.GetExp() != 1_700_000_000+300 {
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
	if c.Image != "example.test/pigeon:1" || *job.Spec.BackoffLimit != 0 || pod.RestartPolicy != corev1.RestartPolicyNever ||
		*job.Spec.ActiveDeadlineSeconds != 600+30 || *job.Spec.TTLSecondsAfterFinished != 60 {
		t.Fatalf("job spec %+v", job.Spec)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("a pigeon must hold no Kubernetes identity")
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

	// Stop deletes the Job and Secret and revokes the identity.
	if err := r.Stop(ctx, cfg, run); err != nil {
		t.Fatal(err)
	}
	if _, err := client.BatchV1().Jobs(cfg.PigeonsNamespace).Get(ctx, "pigeon-01abc", metav1.GetOptions{}); err == nil {
		t.Fatal("job survived Stop")
	}
	if _, err := client.CoreV1().Secrets(cfg.PigeonsNamespace).Get(ctx, "pigeon-01abc", metav1.GetOptions{}); err == nil {
		t.Fatal("secret survived Stop")
	}
	if len(mint.revoked) != 1 || mint.revoked[0] != id {
		t.Fatalf("revoked %v", mint.revoked)
	}
	// Stopping again is a no-op, and a gone Job reads as ended.
	if err := r.Stop(ctx, cfg, run); err != nil {
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
