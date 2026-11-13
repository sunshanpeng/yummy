package clientcli

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func (a *clientApp) runtimeConfigCommand() *cobra.Command {
	root := &cobra.Command{Use: "runtime-config", Short: "读取或保存应用运行配置（保存不会部署）"}
	var revision int64
	get := &cobra.Command{Use: "get", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		w, id, err := requireTarget(p)
		if err != nil {
			return err
		}
		r, err := c.runtime.GetRuntimeConfig(cmd.Context(), auth(c.token, &platformv1.GetRuntimeConfigRequest{WorkspaceId: w, ApplicationId: id, Revision: revision}))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	get.Flags().Int64Var(&revision, "revision", 0, "指定历史配置版本，0 为当前")
	var file, key string
	save := &cobra.Command{Use: "save", Short: "从 JSON 文件或标准输入保存完整配置", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, p, err := a.client()
		if err != nil {
			return err
		}
		w, id, err := requireTarget(p)
		if err != nil {
			return err
		}
		if file == "" {
			return fail("INVALID_ARGUMENT", 2, false, "必须使用 --file 指定 JSON 文件，或使用 - 从标准输入读取")
		}
		var data []byte
		if file == "-" {
			data, err = readBounded(cmd.InOrStdin(), 4<<20)
		} else {
			var f *os.File
			f, err = os.Open(file)
			if err == nil {
				defer f.Close()
				data, err = readBounded(f, 4<<20)
			}
		}
		if err != nil {
			return configError(errors.New("读取配置失败或内容过大"))
		}
		var input platformv1.SaveRuntimeConfigRequest
		if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, &input); err != nil {
			return fail("INVALID_ARGUMENT", 2, false, "运行配置 JSON 格式错误或含未知字段")
		}
		if (input.WorkspaceId != "" && input.WorkspaceId != w) || (input.ApplicationId != "" && input.ApplicationId != id) {
			return fail("INVALID_ARGUMENT", 2, false, "配置中的资源身份与命令上下文不一致")
		}
		input.WorkspaceId = w
		input.ApplicationId = id
		if key != "" && input.IdempotencyKey != "" && key != input.IdempotencyKey {
			return fail("INVALID_ARGUMENT", 2, false, "幂等键不一致")
		}
		if key == "" {
			key = input.IdempotencyKey
		}
		input.IdempotencyKey, err = mutationKey(cmd, key)
		if err != nil {
			return err
		}
		r, err := c.runtime.SaveRuntimeConfig(cmd.Context(), auth(c.token, &input))
		if err != nil {
			return rpcError(err)
		}
		return output(cmd, r.Msg)
	}}
	save.Flags().StringVar(&file, "file", "", "配置 JSON 路径或 -；敏感配置请使用安全管道")
	save.Flags().StringVar(&key, "idempotency-key", "", "固定幂等键")
	root.AddCommand(get, save)
	return root
}
