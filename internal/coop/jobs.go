// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package coop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/tacplus-dev/coop/tacplus/chassis/token"
	pb "github.com/tacplus-dev/coop/tacplus/types/services/coop"
	kernel_pb "github.com/tacplus-dev/coop/tacplus/types/services/kernel"
	tokenrequest_pb "github.com/tacplus-dev/coop/tacplus/types/sys/tokenrequest"
)

// Labels on every pigeon Job and Secret; the run label is what reconciliation
// lists by. A tenant namespace is a path, which a label value cannot hold, so
// it rides an annotation.
const (
	labelRun         = "coop.tacplus.dev/run"
	labelKind        = "coop.tacplus.dev/kind"
	annotationTenant = "coop.tacplus.dev/tenant"

	// pigeonMount is where the Job finds its bootstrap token and config.
	pigeonMount = "/run/pigeon"
	// bootstrapTokenMax is kernel's cap on a workload bootstrap token's life.
	bootstrapTokenMax = 300 * time.Second
	// readyGrace is added to a Job's activeDeadlineSeconds past the run's
	// deadline: the coop ends the run at the deadline itself; this is the
	// backstop if the coop is not there to.
	readyGrace = 30 * time.Second
)

// Minter is the part of kernel's Mint client the runner uses.
type Minter interface {
	MintWorkload(ctx context.Context, req *tokenrequest_pb.WorkloadMintRequest, opts ...grpc.CallOption) (*tokenrequest_pb.WorkloadMintResponse, error)
	RevokeWorkload(ctx context.Context, req *kernel_pb.WorkloadRevokeRequest, opts ...grpc.CallOption) error
}

// JobRunner is MODE_JOBS: one Kubernetes Job per pigeon in the pigeons
// namespace, with an identity kernel mints for the run (A30 §3.2, §3.3).
type JobRunner struct {
	Client kubernetes.Interface
	Mint   Minter
	// Bootstrap is the coop's own bootstrap config (BOOTSTRAP_CONFIG): the
	// same cluster, CAs and kernel endpoints a pigeon needs, copied into each
	// run's Secret.
	Bootstrap []byte
	Now       func() time.Time
}

func (r *JobRunner) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func jobName(run *pb.LiveRun) string { return "pigeon-" + run.GetRunId() }

// Start mints the pigeon's identity, writes the run's Secret and creates its
// Job. The pigeon's SPIFFE id is the minted token's subject.
func (r *JobRunner) Start(ctx context.Context, cfg *pb.Config, run *pb.LiveRun, kind *pb.Kind, _ []byte) (string, error) {
	ns := cfg.GetPigeonsNamespace()
	if ns == "" {
		return "", errors.New("coop config names no pigeons_namespace")
	}
	if kind.GetImage() == "" {
		return "", fmt.Errorf("kind %s has no image", kind.GetName())
	}
	limits, err := kindLimits(kind)
	if err != nil {
		return "", err
	}
	now := r.now()
	exp := now.Add(bootstrapTokenMax)
	if deadline := time.UnixMilli(run.GetDeadlineMs()); deadline.Before(exp) {
		exp = deadline
	}
	minted, err := r.Mint.MintWorkload(ctx, &tokenrequest_pb.WorkloadMintRequest{
		WorkloadName: jobName(run), Terminal: true, Exp: exp.Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("minting the pigeon's identity: %w", err)
	}
	bootstrap := token.NewImmutableToken(minted.GetBootstrapToken())
	pigeonID := bootstrap.GetSubject()
	if pigeonID == "" || bootstrap.GetBase64() == "" {
		return "", errors.New("kernel minted a workload token with no subject or no encoded form")
	}

	name := jobName(run)
	labels := map[string]string{labelRun: run.GetRunId(), labelKind: run.GetKind()}
	annotations := map[string]string{annotationTenant: run.GetTenantNamespace()}
	activeDeadline := int64(time.UnixMilli(run.GetDeadlineMs()).Sub(now)/time.Second) + int64(readyGrace/time.Second)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(activeDeadline),
			TTLSecondsAfterFinished: ptr.To(int32(60)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: []corev1.Volume{
						{Name: "pigeon", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: ptr.To(int32(0o400))}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
					Containers: []corev1.Container{{
						Name:  "pigeon",
						Image: kind.GetImage(),
						Env: []corev1.EnvVar{
							{Name: "KERNEL_BOOTSTRAP_TOKEN_FILE", Value: pigeonMount + "/bootstrap.token"},
							{Name: "BOOTSTRAP_CONFIG", Value: pigeonMount + "/bootstrap.yaml"},
							{Name: "PIGEON_RUN_ID", Value: run.GetRunId()},
							{Name: "PIGEON_THREAD_ID", Value: run.GetThreadId()},
							{Name: "PIGEON_TENANT", Value: run.GetTenantNamespace()},
							{Name: "PIGEON_KIND", Value: run.GetKind()},
							{Name: "PIGEON_CALLER", Value: run.GetCallerSpiffeId()},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "pigeon", MountPath: pigeonMount, ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
						},
						Resources: corev1.ResourceRequirements{Limits: limits, Requests: limits},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}
	created, err := r.Client.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating the run's job: %w", err)
	}
	// The Job first, then its Secret born owned by it, so it follows the Job
	// out and the coop never needs to update a Secret. The pod waits on the
	// mount until the Secret exists, which is a moment later.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: created.Name, UID: created.UID, Controller: ptr.To(true),
			}}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"bootstrap.token": []byte(bootstrap.GetBase64()),
			"bootstrap.yaml":  r.Bootstrap,
		},
	}
	if _, err := r.Client.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		policy := metav1.DeletePropagationForeground
		_ = r.Client.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
		return "", fmt.Errorf("creating the run's secret: %w", err)
	}
	run.JobName = name
	return pigeonID, nil
}

// Stop deletes the run's Job (foreground, so its pod and Secret go with it)
// and revokes the pigeon's identity where kernel lets this coop.
func (r *JobRunner) Stop(ctx context.Context, cfg *pb.Config, run *pb.LiveRun) error {
	ns := cfg.GetPigeonsNamespace()
	name := jobName(run)
	policy := metav1.DeletePropagationForeground
	if err := r.Client.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting job %s: %w", name, err)
	}
	if err := r.Client.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting secret %s: %w", name, err)
	}
	if id := run.GetPigeonSpiffeId(); id != "" {
		if err := r.Mint.RevokeWorkload(ctx, &kernel_pb.WorkloadRevokeRequest{SpiffeId: id}); err != nil {
			// Not fatal: the pod is gone and the token's TTL bounds the rest.
			return fmt.Errorf("revoking %s (pod deleted; token expires at its ttl): %w", id, err)
		}
	}
	return nil
}

// Check reports whether the run's Job has finished on its own: a failed Job
// (the pigeon crashed, or Kubernetes gave up on it) ends the run FAILED; a Job
// that completed ends it DONE, the pigeon having exited after its result.
func (r *JobRunner) Check(ctx context.Context, cfg *pb.Config, run *pb.LiveRun) (bool, pb.EndReason, string, error) {
	job, err := r.Client.BatchV1().Jobs(cfg.GetPigeonsNamespace()).Get(ctx, jobName(run), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, pb.EndReason_END_REASON_FAILED, "job gone", nil
	}
	if err != nil {
		return false, pb.EndReason_END_REASON_UNSPECIFIED, "", err
	}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobFailed:
			return true, pb.EndReason_END_REASON_FAILED, "job failed: " + c.Reason, nil
		case batchv1.JobComplete:
			return true, pb.EndReason_END_REASON_DONE, "pigeon exited", nil
		}
	}
	return false, pb.EndReason_END_REASON_UNSPECIFIED, "", nil
}

// Reconcile is the coop's start: a Job with no live run is deleted, and the
// live runs with no Job are returned for the coop to end FAILED.
func (r *JobRunner) Reconcile(ctx context.Context, cfg *pb.Config, live map[string]*pb.LiveRun) ([]*pb.LiveRun, error) {
	ns := cfg.GetPigeonsNamespace()
	jobs, err := r.Client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: labelRun})
	if err != nil {
		return nil, fmt.Errorf("listing pigeon jobs: %w", err)
	}
	have := map[string]bool{}
	for _, j := range jobs.Items {
		id := j.Labels[labelRun]
		run, ok := live[id]
		if ok && run.GetState() != pb.RunState_RUN_STATE_ENDED {
			have[id] = true
			continue
		}
		policy := metav1.DeletePropagationForeground
		if err := r.Client.BatchV1().Jobs(ns).Delete(ctx, j.Name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("deleting orphan job %s: %w", j.Name, err)
		}
	}
	var missing []*pb.LiveRun
	for id, run := range live {
		if run.GetState() == pb.RunState_RUN_STATE_ENDED || run.GetState() == pb.RunState_RUN_STATE_REQUESTED {
			continue
		}
		if !have[id] {
			missing = append(missing, run)
		}
	}
	return missing, nil
}

func kindLimits(kind *pb.Kind) (corev1.ResourceList, error) {
	limits := corev1.ResourceList{}
	if kind.GetCpu() != "" {
		q, err := resource.ParseQuantity(kind.GetCpu())
		if err != nil {
			return nil, fmt.Errorf("kind %s cpu %q: %w", kind.GetName(), kind.GetCpu(), err)
		}
		limits[corev1.ResourceCPU] = q
	}
	if kind.GetMemory() != "" {
		q, err := resource.ParseQuantity(kind.GetMemory())
		if err != nil {
			return nil, fmt.Errorf("kind %s memory %q: %w", kind.GetName(), kind.GetMemory(), err)
		}
		limits[corev1.ResourceMemory] = q
	}
	return limits, nil
}
