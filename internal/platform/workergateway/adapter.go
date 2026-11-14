package workergateway

import (
	"context"
	"crypto/x509"
	"errors"
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/platform/build"
	"github.com/sunshanpeng/yummy/internal/platform/deployment"
	"github.com/sunshanpeng/yummy/internal/platform/enrollment"
)

func buildIdentity(value enrollment.Identity) build.WorkerIdentity {
	return build.WorkerIdentity{NodeID: value.NodeID, WorkerID: value.WorkerID, CertificateSerial: value.Serial}
}
func deploymentIdentity(value enrollment.Identity, certificate *x509.Certificate) deployment.WorkerIdentity {
	return deployment.WorkerIdentity{NodeID: value.NodeID, WorkerID: value.WorkerID, CertificateSerial: value.Serial, CertificateSHA256: certificateHash(certificate)}
}
func (g *Gateway) receive(ctx context.Context, s *session, message *platformv1.WorkerEnvelope) error {
	switch payload := message.Payload.(type) {
	case *platformv1.WorkerEnvelope_Heartbeat:
		return g.heartbeat(ctx, s, payload.Heartbeat)
	case *platformv1.WorkerEnvelope_Qualification:
		return g.qualification(ctx, s, payload.Qualification)
	case *platformv1.WorkerEnvelope_BuildEvent:
		if g.config.Builds == nil {
			return ErrProtocol
		}
		value := payload.BuildEvent
		if value == nil {
			return ErrProtocol
		}
		ack, err := g.config.Builds.ApplyEvent(ctx, buildIdentity(s.identity), build.Event{AttemptID: value.AttemptId, Seq: value.Seq, Kind: value.Kind, JobUID: value.JobUid, Stage: value.Stage, Digest: value.Digest, ErrorCode: value.ErrorCode, LogsComplete: value.LogsComplete})
		if err != nil {
			return g.reject(s, "build-event", value.AttemptId, value.Seq, err)
		}
		return g.ack(s, "build-event", value.AttemptId, ack.Seq, ack.Duplicate, ack.State)
	case *platformv1.WorkerEnvelope_BuildLog:
		if g.config.Builds == nil {
			return ErrProtocol
		}
		value := payload.BuildLog
		if value == nil || len(value.Content) > build.MaxLogChunk {
			return ErrProtocol
		}
		ack, err := g.config.Builds.AppendLogs(ctx, buildIdentity(s.identity), build.LogInput{AttemptID: value.AttemptId, Seq: value.Seq, Content: value.Content})
		if err != nil {
			return g.reject(s, "build-log", value.AttemptId, value.Seq, err)
		}
		return g.ack(s, "build-log", value.AttemptId, ack.Seq, ack.Duplicate, ack.State)
	case *platformv1.WorkerEnvelope_DeploymentObservation:
		if g.config.Deployments == nil {
			return ErrProtocol
		}
		value := payload.DeploymentObservation
		if value == nil {
			return ErrProtocol
		}
		ack, err := g.config.Deployments.ApplyObservation(ctx, deploymentIdentity(s.identity, s.certificate), deployment.Observation{WorkspaceID: value.WorkspaceId, ApplicationID: value.ApplicationId, OperationID: value.OperationId, ReleaseID: value.ReleaseId, Generation: value.Generation, Seq: value.Seq, ClusterNodeUID: value.ClusterNodeUid, DeploymentUID: value.DeploymentUid, PodUID: value.PodUid, Digest: value.Digest, CorrectGeneration: value.CorrectGeneration, ReadyReplicas: value.ReadyReplicas, Replicas: value.Replicas, OldPodsGone: value.OldPodsGone, ServiceEndpoints: value.ServiceEndpoints, Kind: value.Kind, ErrorCode: value.ErrorCode, CollectedAt: time.UnixMilli(value.CollectedUnixMilli).UTC()})
		if err != nil {
			return g.reject(s, "deployment", value.OperationId, value.Seq, err)
		}
		return g.ack(s, "deployment", value.OperationId, ack.Seq, ack.Duplicate, ack.State)
	case *platformv1.WorkerEnvelope_RuntimeLog:
		if g.config.Deployments == nil {
			return ErrProtocol
		}
		value := payload.RuntimeLog
		if value == nil || len(value.Content) > deployment.RuntimeLogMaxChunkBytes {
			return ErrProtocol
		}
		id, err := runtimeLogStreamID(value)
		if err != nil {
			return err
		}
		ack, err := g.config.Deployments.ApplyRuntimeLog(ctx, deploymentIdentity(s.identity, s.certificate), deployment.RuntimeLogInput{WorkspaceID: value.WorkspaceId, ApplicationID: value.ApplicationId, OperationID: value.OperationId, ReleaseID: value.ReleaseId, Generation: value.Generation, PodUID: value.PodUid, Container: value.Container, RestartCount: value.RestartCount, Seq: value.Seq, Content: value.Content, Truncated: value.Truncated})
		if err != nil {
			return g.reject(s, "runtime-log", id, value.Seq, err)
		}
		return g.ack(s, "runtime-log", id, ack.Seq, ack.Duplicate, ack.State)
	case *platformv1.WorkerEnvelope_RuntimeMetric:
		if g.config.Deployments == nil {
			return ErrProtocol
		}
		value := payload.RuntimeMetric
		if value == nil {
			return ErrProtocol
		}
		ack, err := g.config.Deployments.ApplyRuntimeMetric(ctx, deploymentIdentity(s.identity, s.certificate), deployment.RuntimeMetricInput{WorkspaceID: value.WorkspaceId, ApplicationID: value.ApplicationId, ReleaseID: value.ReleaseId, Generation: value.Generation, PodUID: value.PodUid, CPUNanoCores: value.CpuNanoCores, CPUWindowMillis: value.CpuWindowMillis, MemoryWorkingSetBytes: value.MemoryWorkingSetBytes, CollectedAt: time.UnixMilli(value.CollectedUnixMilli).UTC()})
		if err != nil {
			return g.reject(s, "runtime-metric", value.PodUid, value.CollectedUnixMilli, err)
		}
		// ack.Seq 携带采集毫秒时间，worker journal 以同一序号清理待确认条目。
		return g.ack(s, "runtime-metric", value.PodUid, ack.Seq, ack.Duplicate, ack.State)
	default:
		return ErrProtocol
	}
}
func (g *Gateway) ack(s *session, channel, id string, seq int64, duplicate bool, state string) error {
	return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_Ack{Ack: &platformv1.WorkerAck{Channel: channel, ExecutionId: id, Seq: seq, Duplicate: duplicate, State: state}}})
}
func (g *Gateway) reject(s *session, channel, id string, seq int64, err error) error {
	code := "REJECTED"
	if errors.Is(err, build.ErrSequence) || errors.Is(err, deployment.ErrSequence) {
		code = "SEQUENCE_CONFLICT"
	}
	return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_Rejection{Rejection: &platformv1.WorkerRejection{Channel: channel, ExecutionId: id, Seq: seq, Code: code}}})
}
func (g *Gateway) schedule(ctx context.Context, s *session) error {
	if g.config.Builds != nil {
		pending, err := g.config.Builds.PendingDispatch(ctx, buildIdentity(s.identity), 16)
		if err != nil {
			return err
		}
		for _, value := range pending {
			if err := g.sendBuild(ctx, s, value); err != nil {
				return err
			}
		}
		value, err := g.config.Builds.Claim(ctx, buildIdentity(s.identity))
		if err == nil {
			if err := g.sendBuild(ctx, s, value); err != nil {
				return err
			}
		} else if !errors.Is(err, build.ErrNoCapacity) {
			return err
		}
	}
	if g.config.Deployments != nil {
		value, err := g.config.Deployments.ClaimPending(ctx, deploymentIdentity(s.identity, s.certificate))
		if err == nil {
			return g.sendDeployment(ctx, s, value)
		}
		if !errors.Is(err, deployment.ErrNoPending) {
			return err
		}
	}
	return nil
}
func (g *Gateway) sendBuild(ctx context.Context, s *session, value build.Dispatch) error {
	if value.NodeID != s.identity.NodeID || value.WorkerID != s.identity.WorkerID || value.AttemptID == "" || value.JobName == "" {
		return ErrIdentity
	}
	if err := g.check(ctx, s); err != nil {
		return err
	}
	message := &platformv1.WorkerBuildCommand{BuildId: value.BuildID, AttemptId: value.AttemptID, WorkspaceId: value.WorkspaceID, ApplicationId: value.ApplicationID, NodeId: value.NodeID, WorkerId: value.WorkerID, Action: value.Command, StableJobName: value.JobName, JobUid: value.JobUID, SourceSnapshotId: value.SourceSnapshotID, SourceSha256: value.SourceSHA256, Dockerfile: value.Config.Dockerfile, ContextPath: value.Config.Context, Target: value.Config.Target, BuildArgs: value.Config.Args, Repository: value.Repository, Tag: value.Tag, Platform: value.Platform, DeadlineUnix: value.DeadlineAt.Unix(), SpaceSlug: value.SpaceSlug}
	if value.Command == "RUN" {
		if g.config.BuildMaterial == nil {
			return ErrQualification
		}
		material, err := g.config.BuildMaterial.MaterializeBuild(ctx, s.identity, value)
		if err != nil {
			return err
		}
		message.SourceGrant = material.Source
		message.Registry = material.Registry
		message.Secrets = material.Secrets
		if message.SourceGrant == nil || message.Registry == nil {
			return ErrQualification
		}
	}
	if err := g.check(ctx, s); err != nil {
		return err
	}
	return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_BuildCommand{BuildCommand: message}})
}
func (g *Gateway) sendDeployment(ctx context.Context, s *session, value deployment.Dispatch) error {
	if value.Placement.NodeID != s.identity.NodeID || value.Placement.WorkerID != s.identity.WorkerID {
		return ErrIdentity
	}
	if err := g.check(ctx, s); err != nil {
		return err
	}
	message := deploymentCommand(value)
	if value.Action != deployment.Stop {
		if g.config.DeploymentMaterial == nil {
			return ErrQualification
		}
		credential, err := g.config.DeploymentMaterial.MaterializeDeployment(ctx, s.identity, s.certificate, value)
		if err != nil {
			return err
		}
		if credential == nil {
			return ErrQualification
		}
		message.Registry = credential
	}
	if err := g.check(ctx, s); err != nil {
		return err
	}
	return g.enqueue(s, &platformv1.ControlEnvelope{Payload: &platformv1.ControlEnvelope_DeploymentCommand{DeploymentCommand: message}})
}
func deploymentCommand(value deployment.Dispatch) *platformv1.WorkerDeploymentCommand {
	view := value.Config.View
	config := &platformv1.RuntimeConfig{WorkspaceId: value.WorkspaceID, ApplicationId: value.ApplicationID, Revision: view.Revision, Confirmed: view.Confirmed, Type: view.Type, Port: view.Port, Command: view.Command, Args: view.Args, Domains: view.Domains, Probe: &platformv1.RuntimeProbe{Path: view.Probe.Path, InitialDelaySeconds: view.Probe.InitialDelaySeconds, PeriodSeconds: view.Probe.PeriodSeconds, TimeoutSeconds: view.Probe.TimeoutSeconds, FailureThreshold: view.Probe.FailureThreshold}, Resources: &platformv1.RuntimeResources{CpuRequestMilli: view.Resources.CPURequestMilli, CpuLimitMilli: view.Resources.CPULimitMilli, MemoryRequestMib: view.Resources.MemoryRequestMiB, MemoryLimitMib: view.Resources.MemoryLimitMiB}}
	for _, volume := range view.Volumes {
		config.Volumes = append(config.Volumes, &platformv1.RuntimeVolume{Name: volume.Name, MountPath: volume.MountPath, SizeGib: volume.SizeGiB})
	}
	// 原始值只在 materialized 映射中；保留每项的敏感标志，worker 才能分别生成 Secret 与普通配置。
	for _, variable := range view.Variables {
		config.Variables = append(config.Variables, &platformv1.RuntimeVariable{Name: variable.Name, Sensitive: variable.Sensitive, Configured: variable.Configured})
	}
	for _, file := range view.Files {
		config.Files = append(config.Files, &platformv1.RuntimeFile{Path: file.Path, Sensitive: file.Sensitive, Configured: file.Configured})
	}
	message := &platformv1.WorkerDeploymentCommand{WorkspaceId: value.WorkspaceID, ApplicationId: value.ApplicationID, OperationId: value.OperationID, ReleaseId: value.ReleaseID, Action: value.Action, Generation: value.Generation, NodeId: value.Placement.NodeID, WorkerId: value.Placement.WorkerID, ClusterNodeUid: value.Placement.ClusterNodeUID, Repository: value.Artifact.Repository, Digest: value.Artifact.Digest, Platform: value.Artifact.Platform, RestartMarker: value.RestartMarker, Config: config, MaterializedVariables: value.Config.Variables, MaterializedFiles: value.Config.Files, SpaceSlug: value.SpaceSlug, AppName: value.AppName}
	// 入口 TLS 是控制面领取时汇聚的当下绑定；未一致绑定时如实下发 https=false，worker 不猜证书。
	if value.TLS != nil {
		message.Https = value.TLS.HTTPS
		message.TlsSecretName = value.TLS.SecretName
	}
	return message
}
