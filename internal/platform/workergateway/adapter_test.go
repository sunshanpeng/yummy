package workergateway

import (
	"testing"

	"github.com/sunshanpeng/yummy/internal/platform/deployment"
	"github.com/sunshanpeng/yummy/internal/platform/runtimeconfig"
)

func TestDeploymentCommandPreservesSensitiveMetadata(t *testing.T) {
	value := deployment.Dispatch{WorkspaceID: "space", ApplicationID: "app", Config: runtimeconfig.Materialized{View: runtimeconfig.View{Variables: []runtimeconfig.Variable{{Name: "SECRET", Sensitive: true, Configured: true}, {Name: "MODE"}}, Files: []runtimeconfig.ConfigFile{{Path: "/run/config", Sensitive: true, Configured: true}}}, Variables: map[string]string{"SECRET": "private-value", "MODE": "prod"}, Files: map[string]string{"/run/config": "private-file"}}}
	command := deploymentCommand(value)
	if len(command.Config.Variables) != 2 || !command.Config.Variables[0].Sensitive || command.Config.Variables[0].Value != "" || command.Config.Variables[1].Sensitive || len(command.Config.Files) != 1 || !command.Config.Files[0].Sensitive || command.Config.Files[0].Content != "" {
		t.Fatalf("变量/文件敏感元数据丢失或明文重复: %+v", command.Config)
	}
	if command.MaterializedVariables["SECRET"] != "private-value" || command.MaterializedFiles["/run/config"] != "private-file" {
		t.Fatal("内部执行材料没有正确交付")
	}
}

// 入口 TLS 汇聚事实只按命令映射：TLS=nil 时如实下发 https=false 与空 Secret。
func TestDeploymentCommandMapsTLSFact(t *testing.T) {
	value := deployment.Dispatch{TLS: &deployment.DispatchTLS{HTTPS: true, SecretName: "wildcard-example"}}
	command := deploymentCommand(value)
	if !command.Https || command.TlsSecretName != "wildcard-example" {
		t.Fatalf("TLS绑定映射丢失: %+v", command)
	}
	value.TLS = nil
	command = deploymentCommand(value)
	if command.Https || command.TlsSecretName != "" {
		t.Fatalf("未绑定不得伪造TLS字段: %+v", command)
	}
}
